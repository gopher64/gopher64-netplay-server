#include "udpServer.h"
#define XXH_INLINE_ALL
#include "xxhash.h"
#include <QNetworkDatagram>
#include <QtEndian>
#include <QTimer>

UdpServer::UdpServer(char _buffer_target)
{
    timerId = 0;
    for (int i = 0; i < 4; ++i)
    {
        lead_count[i] = 0;
        buffer_size[i] = 3;
        buffer_health[i] = -1;
        input_delay[i] = -1;
        sender_address[i] = QHostAddress();
        sender_port[i] = 0;
    }
    status = 0;
    buffer_target = _buffer_target;
}

void UdpServer::setPort(int _port)
{
    udpSocket.bind(QHostAddress::Any, _port);
    port = _port;

    connect(&udpSocket, &QUdpSocket::readyRead,
            this, &UdpServer::readPendingDatagrams);
}

void UdpServer::close()
{
    udpSocket.close();
}

int UdpServer::getPort()
{
    return port;
}

void UdpServer::setInputDelay(int playerNum, int inputDelay)
{
    input_delay[playerNum] = inputDelay;
}

bool UdpServer::checkIfExists(quint8 playerNumber, quint32 count)
{
    bool inputExists = inputs[playerNumber].contains(count);
    inputs[playerNumber].remove(count - 5000);

    if (input_delay[playerNumber] < 0 && !inputExists)
    {
        if (!buttons[playerNumber].isEmpty())
            inputs[playerNumber].insert(count, buttons[playerNumber].takeFirst());
        else if (inputs[playerNumber].contains(count-1))
            inputs[playerNumber].insert(count, inputs[playerNumber].value(count-1));
        else
            inputs[playerNumber].insert(count, qMakePair(0, 0/*Controller not present*/));

        return true;
    } else
        // When using input delay, we must wait for inputs
        return inputExists;
}

void UdpServer::sendInput(quint32 count, QHostAddress address, int port, quint8 playerNum, quint8 spectator)
{
    char buffer[512];
    quint32 count_lag = lead_count[playerNum] - count;
    buffer[0] = 1; // Key info from server
    buffer[1] = playerNum;
    buffer[2] = status;
    buffer[3] = (quint8) count_lag;
    quint32 curr = 5;
    quint32 start = count;
    quint32 end = start + buffer_size[playerNum];

    while ( (curr < 500) && ( (spectator == 0 && count_lag == 0 && (count - end) > (UINT32_MAX / 2)) || inputs[playerNum].contains(count) ) )
    {
        qToBigEndian(count, &buffer[curr]);
        curr += 4;
        if (!checkIfExists(playerNum, count))
        {
            // we don't have an input for this frame yet
            end = count - 1;
            continue;
        }
        qToBigEndian(inputs[playerNum].value(count).first, &buffer[curr]);
        curr += 4;
        buffer[curr] = inputs[playerNum].value(count).second;
        curr += 1;
        ++count;
    }

    buffer[4] = count - start; //number of counts in packet

    if (curr > 5)
        udpSocket.writeDatagram(&buffer[0], curr, address, port);
}

void UdpServer::register_player(quint32 reg_id, quint8 playerNum, quint8 plugin)
{
    player_keepalive[reg_id].first = 0;
    player_keepalive[reg_id].second = playerNum;
    inputs[playerNum].insert(0, qMakePair(0, plugin));
    emit writeLog("Player " + QString::number(playerNum + 1) + " registered", port);
    if (timerId == 0)
        timerId = startTimer(500);
}

void UdpServer::readPendingDatagrams()
{
    quint32 keys, count, vi_count, regi_id;
    quint8 playerNum, spectator, i;
    while (udpSocket.hasPendingDatagrams())
    {
        QNetworkDatagram datagram = udpSocket.receiveDatagram();
        QByteArray incomingData = datagram.data();
        playerNum = incomingData.at(1);
        switch (incomingData.at(0))
        {
            case 0: // key info from client
                sender_address[playerNum] = datagram.senderAddress();
                sender_port[playerNum] = datagram.senderPort();
                count = qFromBigEndian<quint32>(&incomingData.data()[2]);
                keys = qFromBigEndian<quint32>(&incomingData.data()[6]);
                if (input_delay[playerNum] >= 0)
                {
                    QPair<quint32, quint8> pair = qMakePair(keys, incomingData.at(10));
                    insertInput(playerNum, count + input_delay[playerNum], &pair);
                }
                else if (buttons[playerNum].size() == 0)
                    buttons[playerNum].append(qMakePair(keys, incomingData.at(10)));

                for (i = 0; i < 4; ++i)
                {
                    if (sender_port[i])
                        sendInput(count, sender_address[i], sender_port[i], playerNum, 1);
                }
                break;
            case 2: // request for player input data
                regi_id = qFromBigEndian<quint32>(&incomingData.data()[2]);
                if (player_keepalive.contains(regi_id))
                    player_keepalive[regi_id].first = 0;

                count = qFromBigEndian<quint32>(&incomingData.data()[6]);
                spectator = incomingData.at(10);
                if (((count - lead_count[playerNum]) < (UINT32_MAX / 2)) && spectator == 0)
                {
                    buffer_health[playerNum] = incomingData.at(11);
                    lead_count[playerNum] = count;
                }
                sendInput(count, datagram.senderAddress(), datagram.senderPort(), playerNum, spectator);
                break;
            case 4: // cp0 info from client
                if ((status & 1) == 0)
                {
                    vi_count = qFromBigEndian<quint32>(&incomingData.data()[1]);
                    if (!sync_hash.contains(vi_count))
                    {
                        if (sync_hash.size() > 500)
                            sync_hash.clear();
                        sync_hash.insert(vi_count, XXH3_64bits(&incomingData.data()[5], 128));
                    }
                    else if (sync_hash.value(vi_count) != XXH3_64bits(&incomingData.data()[5], 128))
                    {
                        status |= 1;
                        emit desynced();
                    }
                }
                break;
            default:
                printf("Unknown packet type %d\n", incomingData.at(0));
                break;
        }
    }
}

void UdpServer::timerEvent(QTimerEvent *)
{
    for (int i = 0; i < 4; ++i)
    {
        if (buffer_health[i] != -1)
        {
            if (buffer_health[i] > buffer_target && buffer_size[i] > 0)
                --buffer_size[i];
            else if (buffer_health[i] < buffer_target)
                ++buffer_size[i];
        }
    }

    quint32 should_delete = 0;
    QHash<quint32, QPair<quint8, quint8>>::iterator iter;
    for (iter = player_keepalive.begin(); iter != player_keepalive.end(); ++iter)
    {
        ++iter.value().first;
        if (iter.value().first > 40)
            should_delete = iter.key();
    }

    if (should_delete)
        disconnect_player(should_delete);
}

void UdpServer::disconnect_player(quint32 reg_id)
{
    if (!player_keepalive.contains(reg_id))
        return;

    emit writeLog("Player " + QString::number(player_keepalive.value(reg_id).second + 1) + " disconnected", port);

    status |= (0x1 << (player_keepalive.value(reg_id).second + 1));
    player_keepalive.remove(reg_id);

    if (player_keepalive.isEmpty())
        emit killMe(port);
}

void UdpServer::insertInput(int playerNum, int count, QPair<quint32, quint8> *pair)
{
    int previousCount = count - 1;

    inputs[playerNum].insert(count, *pair);

    /* The recursion here covers two situations:
     *
     * 1. The count < inputDelay, so we need to populate the first frames
     * 2. We lost a udp packet, or received them out of order.
     */
    if (previousCount == 0 || (previousCount > 0 && !inputs[playerNum].contains(previousCount)))
        insertInput(playerNum, previousCount, pair);
}
